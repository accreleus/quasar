package compat

// compat_db_test.go — amendment 56 (#1066): the catalogue, the fleet default and the admin gate,
// against a real database (make test-db). Semantics: control-api.md §Compatibility tools.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/migrate"
	"github.com/accreleus/quasar/control-plane/migrations"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
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
	if _, err := pool.Exec(ctx, `
		UPDATE instance_settings SET compat_tool_default_id = NULL;
		DELETE FROM compat_tool_mappings; DELETE FROM compat_tools;
		DELETE FROM admin_activity; DELETE FROM auth_tokens; DELETE FROM users`); err != nil {
		pool.Close()
		t.Fatalf("reset: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `UPDATE instance_settings SET compat_tool_default_id = NULL;
			DELETE FROM compat_tool_mappings`)
	})
	return pool
}

func TestReconcileIsIdempotentAndKeepsAdminState(t *testing.T) {
	pool := testDB(t)
	s := NewStore(pool)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.Reconcile(ctx); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	items, def, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if def != nil {
		t.Errorf("default = %v on a fresh install, want none", *def)
	}
	var names []string
	for _, it := range items {
		if it.Family == "valve" {
			names = append(names, *it.SteamName)
			if !it.Enabled || it.Asset != nil || it.Hosts != nil {
				t.Errorf("%s: enabled=%v asset=%v hosts=%v, want enabled with no asset or host counts", *it.SteamName, it.Enabled, it.Asset, it.Hosts)
			}
		}
	}
	if got, want := strings.Join(names, ","), "proton_experimental,proton_hotfix,proton_11,proton_10,proton_9,proton_8,proton_7"; got != want {
		t.Errorf("valve order = %s, want %s", got, want)
	}
	last := items[len(items)-1]
	if last.Family != "ge" || last.Enabled || !last.Recommended || last.Asset == nil || last.Asset.URL !=
		"https://github.com/GloriousEggroll/proton-ge-custom/releases/download/GE-Proton11-12/GE-Proton11-12-x86_64.tar.gz" {
		t.Errorf("recommended GE row = %+v, want disabled, recommended, with its derived url", last)
	}

	// An admin's choice survives the next boot.
	if _, err := pool.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_7'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var enabled bool
	if err := pool.QueryRow(ctx, `SELECT enabled FROM compat_tools WHERE steam_name = 'proton_7'`).Scan(&enabled); err != nil || enabled {
		t.Errorf("reconcile re-enabled a tool an admin disabled (enabled=%v err=%v)", enabled, err)
	}
}

type harness struct {
	t     *testing.T
	pool  *pgxpool.Pool
	srv   *httptest.Server
	admin string // bearer
	user  string
	id    map[string]string // steam_name -> id
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool := testDB(t)
	ctx := context.Background()
	if err := NewStore(pool).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	svc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	login := func(name, role string) string {
		u, err := svc.Register(ctx, name+"@t.local", name, "password12345")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id::text = $1`, u.ID, role); err != nil {
			t.Fatal(err)
		}
		tok, err := svc.Login(ctx, name+"@t.local", "password12345", "test")
		if err != nil {
			t.Fatal(err)
		}
		return tok.Plaintext
	}
	ah := auth.NewHandler(svc)
	mux := http.NewServeMux()
	NewHandler(NewStore(pool), audit.NewStore(pool)).Register(mux, func(next http.Handler) http.Handler {
		return ah.RequireAuth(ah.RequireAdmin(next))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{t: t, pool: pool, srv: srv, admin: login("cadmin", "admin"), user: login("cuser", "user"), id: map[string]string{}}
	rows, err := pool.Query(ctx, `SELECT steam_name, id::text FROM compat_tools`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n, id string
		if err := rows.Scan(&n, &id); err != nil {
			t.Fatal(err)
		}
		h.id[n] = id
	}
	return h
}

func (h *harness) do(method, path, token, body string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestAdminRoutesAreAdminOnly(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/admin/compat-tools", ""},
		{"PUT", "/v1/admin/compat-tools/default", `{"tool_id":null}`},
	} {
		if code, _ := h.do(c.method, c.path, "", c.body); code != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: %d, want 401", c.method, c.path, code)
		}
		if code, _ := h.do(c.method, c.path, h.user, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s as a user: %d, want 403", c.method, c.path, code)
		}
	}
	var def *string
	if err := h.pool.QueryRow(context.Background(), `SELECT compat_tool_default_id::text FROM instance_settings`).Scan(&def); err == nil && def != nil {
		t.Error("a refused PUT changed the default")
	}
}

func TestSetDefaultAuditsAndClears(t *testing.T) {
	h := newHarness(t)
	ten := h.id["proton_10"]

	code, body := h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":"`+ten+`"}`)
	if code != http.StatusOK || body["default_tool_id"] != ten {
		t.Fatalf("set default: %d %v", code, body)
	}
	code, body = h.do("GET", "/v1/admin/compat-tools", h.admin, "")
	if code != http.StatusOK || body["default_tool_id"] != ten {
		t.Fatalf("list: %d default=%v, want %s", code, body["default_tool_id"], ten)
	}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		if (m["id"] == ten) != (m["is_default"] == true) {
			t.Errorf("is_default wrong for %v", m["steam_name"])
		}
	}

	// Setting the same value again is a no-op and writes no audit row.
	h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":"`+ten+`"}`)
	code, _ = h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":null}`)
	if code != http.StatusOK {
		t.Fatalf("clear: %d", code)
	}
	rows, err := h.pool.Query(context.Background(), `SELECT action, target_type, details FROM admin_activity ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var action, target string
		var raw []byte
		if err := rows.Scan(&action, &target, &raw); err != nil {
			t.Fatal(err)
		}
		got = append(got, action+"/"+target+" "+string(raw))
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "compat_tool.default.set/instance") ||
		!strings.Contains(got[0], ten) || !strings.Contains(got[1], `"tool_id": null`) || !strings.Contains(got[1], ten) {
		t.Errorf("audit rows = %q, want a set then a clear, each naming the tool", got)
	}
}

func TestSetDefaultRefusals(t *testing.T) {
	h := newHarness(t)
	ge := h.id["GE-Proton11-12-x86_64"]
	if _, err := h.pool.Exec(context.Background(), `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_9'`); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"a GE tool nobody enabled": {`{"tool_id":"` + ge + `"}`, http.StatusConflict},
		"a disabled Valve tool":    {`{"tool_id":"` + h.id["proton_9"] + `"}`, http.StatusConflict},
		"an unknown tool":          {`{"tool_id":"00000000-0000-4000-8000-000000000000"}`, http.StatusNotFound},
		"a malformed id":           {`{"tool_id":"proton_10"}`, http.StatusBadRequest},
		"a missing key":            {`{}`, http.StatusBadRequest},
		"an unknown key":           {`{"tool_id":null,"x":1}`, http.StatusBadRequest},
		"not json":                 {`nope`, http.StatusBadRequest},
	} {
		code, body := h.do("PUT", "/v1/admin/compat-tools/default", h.admin, c.body)
		if code != c.code {
			t.Errorf("%s: %d %v, want %d", name, code, body, c.code)
		}
	}
	if code, body := h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":"`+h.id["proton_9"]+`"}`); code != 409 ||
		!strings.Contains(strings.ToLower(jsonOf(body)), "compat_tool_disabled") {
		t.Errorf("disabled tool: %d %v, want 409 compat_tool_disabled", code, body)
	}
	var n int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM admin_activity`).Scan(&n); err != nil || n != 0 {
		t.Errorf("%d audit rows after refused writes (err %v), want 0", n, err)
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestEntriesAreTheDefaultThenEveryUsableMapping(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s := NewStore(h.pool)
	must := func(_ any, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.pool.Exec(ctx, `INSERT INTO instance_settings (id) VALUES (true) ON CONFLICT DO NOTHING`))
	if got, err := s.Entries(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no default and no mappings: %v %v, want none", got, err)
	}
	must(h.pool.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = $1::uuid`, h.id["proton_10"]))
	must(h.pool.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced) VALUES
		('steam','1145360',$1::uuid,true), ('steam','480',$2::uuid,false), ('steam','220',$3::uuid,false)`,
		h.id["proton_hotfix"], h.id["proton_experimental"], h.id["proton_9"]))
	must(h.pool.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_9'`))

	got, err := s.Entries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{"0", "proton_10", false}, {"1145360", "proton_hotfix", true}, {"480", "proton_experimental", false}}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %+v (the disabled tool's mapping must be left out)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
