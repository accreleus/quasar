package compat

// compat_db_test.go — amendment 56 (#1066): the catalogue, the fleet default and the admin gate,
// against a real database (make test-db). Semantics: control-api.md §Compatibility tools.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
	"github.com/accreleus/quasar/control-plane/internal/jobs"
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
		UPDATE instance_settings SET compat_tool_default_id = NULL, compat_tools_max_enabled = 8;
		DELETE FROM sessions WHERE app_id IN (SELECT id FROM apps WHERE name = 'compat-app');
		DELETE FROM apps WHERE name = 'compat-app';
		DELETE FROM job_runs WHERE job_id = 'compat_tools.converge';
		DELETE FROM host_compat_tools; DELETE FROM hosts WHERE node_name LIKE 'compat-%';
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
	conv  *Converger
	disp  *jobs.Dispatcher
	gh    *fakeGitHub // serves 404 until a test sets gh.serve
	store *Store
	// stopped is the sessions the store's Stopper was asked to stop, in order; stopErr fails it.
	stopped []string
	stopErr error
	// supported is each host's compat_tools_v1, as the agent registry would answer.
	mu        sync.Mutex
	supported map[string]bool
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
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	jobStore := jobs.NewStore(pool)
	conv := NewConverger(pool, jobStore, audit.NewStore(pool), quiet)
	reg := jobs.NewRegistry()
	reg.MustRegister(jobs.Definition{ID: JobID, Name: "Compatibility tools", Plane: jobs.PlaneAgent,
		Scope: jobs.ScopeHost, Managed: true, Default: jobs.Schedule{Kind: jobs.KindEvent},
		FillParams: conv.Params, OnTerminal: conv.Closed})
	if _, err := reg.Sync(ctx, jobStore, "UTC", 50, quiet); err != nil {
		t.Fatal(err)
	}
	disp := jobs.New(jobStore, reg, jobs.DefaultConfig(), quiet)
	conv.Disp = disp
	fake, gh := newFakeGitHub(t, func(_ string, w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	store := NewStore(pool)
	NewHandler(store, audit.NewStore(pool), conv, gh).Register(mux, func(next http.Handler) http.Handler {
		return ah.RequireAuth(ah.RequireAdmin(next))
	})
	jobs.NewAgentHandler(jobStore, disp, nodeSecrets{pool}, quiet).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	h := &harness{t: t, pool: pool, srv: srv, admin: login("cadmin", "admin"), user: login("cuser", "user"),
		id: map[string]string{}, conv: conv, disp: disp, gh: fake, supported: map[string]bool{}, store: store}
	store.Stopper = func(ctx context.Context, sessionID string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.stopErr != nil {
			return h.stopErr
		}
		h.stopped = append(h.stopped, sessionID)
		_, err := pool.Exec(ctx, `UPDATE sessions SET state = 'stopped' WHERE id = $1::uuid`, sessionID)
		return err
	}
	conv.Supported = func(host string) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.supported[host]
	}
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
		{"POST", "/v1/admin/compat-tools", `{"tag":"GE-Proton11-13"}`},
		{"GET", "/v1/admin/compat-tools/available", ""},
		{"PUT", "/v1/admin/compat-tools/default", `{"tool_id":null}`},
		{"GET", "/v1/admin/compat-tools/mappings", ""},
		{"PUT", "/v1/admin/compat-tools/mappings/steam/1145360", `{"tool_id":"00000000-0000-4000-8000-000000000000"}`},
		{"DELETE", "/v1/admin/compat-tools/mappings/steam/1145360", ""},
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
	ctx := context.Background()
	ten := h.id["proton_10"]
	ge, geSHA, _ := h.ge()
	// A second enabled GE tool, so a set can name a GE tool on both sides of its row.
	prevTag, prevSHA, prevName := "GE-Proton11-11", strings.Repeat("ab", 64), "GE-Proton11-11-x86_64"
	var prevGE string
	if err := h.pool.QueryRow(ctx, `INSERT INTO compat_tools (family, source, tag, steam_name, display_name, enabled,
		asset_name, asset_size_bytes, asset_sha512)
		VALUES ('ge', 'tag', $1, $2, $1, true, $1 || '-x86_64.tar.gz', 1, $3) RETURNING id::text`,
		prevTag, prevName, prevSHA).Scan(&prevGE); err != nil {
		t.Fatal(err)
	}
	// Enabled straight in SQL: an enable writes its own audit row, and this test counts them.
	h.exec(`UPDATE compat_tools SET enabled = true WHERE id = $1::uuid`, ge)

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

	// Setting the same value again is audited too: every accepted write is.
	for _, set := range []string{ge, prevGE, prevGE} {
		if code, body := h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":"`+set+`"}`); code != http.StatusOK ||
			body["default_tool_id"] != set {
			t.Fatalf("set default %s: %d %v", set, code, body)
		}
	}
	code, _ = h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":null}`)
	if code != http.StatusOK {
		t.Fatalf("clear: %d", code)
	}
	rows, err := h.pool.Query(ctx, `SELECT action, target_type, details FROM admin_activity ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []map[string]any
	for rows.Next() {
		var action, target string
		var raw []byte
		if err := rows.Scan(&action, &target, &raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		d["action"], d["target_type"] = action, target
		got = append(got, d)
	}
	bothGE := map[string]any{"action": "compat_tool.default.set", "target_type": "instance", "tool_id": prevGE,
		"previous_tool_id": ge, "tag": prevTag, "sha512": prevSHA,
		"previous_tag": recommended[0].Tag, "previous_sha512": geSHA}
	want := []map[string]any{
		// A Valve tool set over nothing has no tag or SHA-512 to name.
		{"action": "compat_tool.default.set", "target_type": "instance", "tool_id": ten, "previous_tool_id": nil},
		// A GE tool names its own tag and SHA-512; the previous tool was Valve, so it names none.
		{"action": "compat_tool.default.set", "target_type": "instance", "tool_id": ge, "previous_tool_id": ten,
			"tag": recommended[0].Tag, "sha512": geSHA},
		bothGE,
		// The same value again is audited too, and names the tool it replaced (itself).
		{"action": "compat_tool.default.set", "target_type": "instance", "tool_id": prevGE, "previous_tool_id": prevGE,
			"tag": prevTag, "sha512": prevSHA, "previous_tag": prevTag, "previous_sha512": prevSHA},
		// A clear names the tool it took the default from.
		{"action": "compat_tool.default.set", "target_type": "instance", "tool_id": nil, "previous_tool_id": prevGE,
			"previous_tag": prevTag, "previous_sha512": prevSHA},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("audit rows = %v, want %v", got, want)
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
		"trailing garbage":         {`{"tool_id":null} x`, http.StatusBadRequest},
		"a stray closing brace":    {`{"tool_id":null}}`, http.StatusBadRequest},
		"a second body":            {`{"tool_id":null}{"tool_id":null}`, http.StatusBadRequest},
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
	if got, _, err := s.Entries(ctx, zeroHost); err != nil || len(got) != 0 {
		t.Fatalf("no default and no mappings: %v %v, want none", got, err)
	}
	must(h.pool.Exec(ctx, `UPDATE instance_settings SET compat_tool_default_id = $1::uuid`, h.id["proton_10"]))
	must(h.pool.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id, forced) VALUES
		('steam','1145360',$1::uuid,true), ('steam','480',$2::uuid,false), ('steam','220',$3::uuid,false)`,
		h.id["proton_hotfix"], h.id["proton_experimental"], h.id["proton_9"]))
	must(h.pool.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_9'`))

	got, _, err := s.Entries(ctx, zeroHost)
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

func (h *harness) mappingAudit() []string {
	h.t.Helper()
	rows, err := h.pool.Query(context.Background(),
		`SELECT action || ' ' || target_type || ' ' || target_id || ' ' || details::text FROM admin_activity ORDER BY id`)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			h.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestMappingSetForcedClearAndAudit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const path = "/v1/admin/compat-tools/mappings/steam/1145360"
	hotfix, nine := h.id["proton_hotfix"], h.id["proton_9"]

	// Unforced by default: forced is absent from the body.
	code, m := h.do("PUT", path, h.admin, `{"tool_id":"`+hotfix+`"}`)
	if code != http.StatusOK || m["forced"] != false || m["tool_id"] != hotfix || m["external_id"] != "1145360" || m["provider"] != "steam" {
		t.Fatalf("set: %d %v", code, m)
	}
	// A second PUT replaces it, never accumulates, and carries the flag.
	if code, m = h.do("PUT", path, h.admin, `{"tool_id":"`+nine+`","forced":true}`); code != http.StatusOK || m["forced"] != true || m["tool_id"] != nine {
		t.Fatalf("replace: %d %v", code, m)
	}
	code, body := h.do("GET", "/v1/admin/compat-tools/mappings", h.admin, "")
	items, _ := body["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["tool_id"] != nine {
		t.Fatalf("list: %d %v, want the one replaced mapping", code, body)
	}

	// The game reaches the session as a forced entry for its appid.
	got, _, err := NewStore(h.pool).Entries(ctx, zeroHost)
	if err != nil || len(got) != 1 || got[0] != (Entry{"1145360", "proton_9", true}) {
		t.Fatalf("entries = %+v %v, want the forced mapping", got, err)
	}

	if code, _ = h.do("DELETE", path, h.admin, ""); code != http.StatusNoContent {
		t.Fatalf("clear: %d", code)
	}
	if code, _ = h.do("DELETE", path, h.admin, ""); code != http.StatusNoContent {
		t.Fatalf("clear again: %d, want 204 also when none existed", code)
	}
	if got, _, err = NewStore(h.pool).Entries(ctx, zeroHost); err != nil || len(got) != 0 {
		t.Fatalf("entries after clear = %+v %v, want none", got, err)
	}

	a := h.mappingAudit()
	if len(a) != 3 ||
		!strings.HasPrefix(a[0], "compat_tool.mapping.set compat_tool_mapping steam:1145360 ") || !strings.Contains(a[0], hotfix) ||
		!strings.Contains(a[1], `"previous_tool_id": "`+hotfix+`"`) || !strings.Contains(a[1], `"forced": true`) ||
		!strings.HasPrefix(a[2], "compat_tool.mapping.clear compat_tool_mapping steam:1145360 ") || !strings.Contains(a[2], `"previous_forced": true`) {
		t.Errorf("audit = %q, want set, replace (with the previous values) and one clear (the second clear changed nothing)", a)
	}
}

func TestMappingWinsOverTheDefaultAndIsKeyedByAppID(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if code, b := h.do("PUT", "/v1/admin/compat-tools/default", h.admin, `{"tool_id":"`+h.id["proton_10"]+`"}`); code != 200 {
		t.Fatalf("default: %d %v", code, b)
	}
	t.Cleanup(func() { _, _ = h.pool.Exec(ctx, `DELETE FROM apps WHERE name = 'tile'`) })
	// Two tiles of one game, and a mapping set before any tile exists for another.
	for i := 0; i < 2; i++ {
		if _, err := h.pool.Exec(ctx, `INSERT INTO apps (name, external_source, external_id) VALUES ('tile', 'steam', '1145360')`); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"1145360", "480"} {
		if code, b := h.do("PUT", "/v1/admin/compat-tools/mappings/steam/"+id, h.admin, `{"tool_id":"`+h.id["proton_hotfix"]+`"}`); code != 200 {
			t.Fatalf("map %s: %d %v", id, code, b)
		}
	}
	var n int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM compat_tool_mappings`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d mapping rows (err %v), want one per appid however many tiles the game has", n, err)
	}
	_, body := h.do("GET", "/v1/admin/compat-tools/mappings", h.admin, "")
	items := body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["external_id"] != "480" || items[1].(map[string]any)["external_id"] != "1145360" {
		t.Fatalf("list = %v, want appids 480 then 1145360 (numeric order)", items)
	}
	if ids := items[1].(map[string]any)["app_ids"].([]any); len(ids) != 2 {
		t.Errorf("app_ids of 1145360 = %v, want both tiles", ids)
	}
	if ids := items[0].(map[string]any)["app_ids"].([]any); len(ids) != 0 {
		t.Errorf("app_ids of 480 = %v, want none: a mapping can precede discovery", ids)
	}

	// The default stays in force for every game, and a game's own mapping is its own entry.
	got, _, err := NewStore(h.pool).Entries(ctx, zeroHost)
	want := []Entry{{"0", "proton_10", false}, {"1145360", "proton_hotfix", false}, {"480", "proton_hotfix", false}}
	if err != nil || len(got) != len(want) {
		t.Fatalf("entries = %+v %v, want %+v", got, err, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Clearing the game's mapping returns it to the default.
	h.do("DELETE", "/v1/admin/compat-tools/mappings/steam/1145360", h.admin, "")
	got, _, _ = NewStore(h.pool).Entries(ctx, zeroHost)
	if len(got) != 2 || got[0].AppID != "0" || got[1].AppID != "480" {
		t.Errorf("entries after clear = %+v, want the default and 480 only", got)
	}
}

func TestMappingRefusals(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	ok := h.id["proton_10"]
	if _, err := h.pool.Exec(ctx, `UPDATE compat_tools SET enabled = false WHERE steam_name = 'proton_9'`); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/admin/compat-tools/mappings/steam/"
	for name, c := range map[string]struct {
		path, body string
		code       int
	}{
		"appid 0 is the default's slot": {path + "0", `{"tool_id":"` + ok + `"}`, 400},
		"a leading zero":                {path + "007", `{"tool_id":"` + ok + `"}`, 400},
		"eleven digits":                 {path + "12345678901", `{"tool_id":"` + ok + `"}`, 400},
		"not a number":                  {path + "abc", `{"tool_id":"` + ok + `"}`, 400},
		"no tool_id":                    {path + "1", `{"forced":true}`, 400},
		"null tool_id":                  {path + "1", `{"tool_id":null}`, 400},
		"forced not a bool":             {path + "1", `{"tool_id":"` + ok + `","forced":"yes"}`, 400},
		"forced null":                   {path + "1", `{"tool_id":"` + ok + `","forced":null}`, 400},
		"an unknown key":                {path + "1", `{"tool_id":"` + ok + `","x":1}`, 400},
		"a malformed id":                {path + "1", `{"tool_id":"proton_10"}`, 400},
		"trailing garbage":              {path + "1", `{"tool_id":"` + ok + `"} x`, 400},
		"an unknown tool":               {path + "1", `{"tool_id":"00000000-0000-4000-8000-000000000000"}`, 404},
		"a disabled tool":               {path + "1", `{"tool_id":"` + h.id["proton_9"] + `"}`, 409},
		"a GE tool nobody enabled":      {path + "1", `{"tool_id":"` + h.id["GE-Proton11-12-x86_64"] + `"}`, 409},
	} {
		if code, b := h.do("PUT", c.path, h.admin, c.body); code != c.code {
			t.Errorf("%s: %d %v, want %d", name, code, b, c.code)
		}
	}
	if code, _ := h.do("DELETE", path+"0", h.admin, ""); code != 400 {
		t.Errorf("DELETE appid 0: %d, want 400", code)
	}
	if code, b := h.do("PUT", path+"1", h.admin, `{"tool_id":"`+h.id["proton_9"]+`"}`); code != 409 ||
		!strings.Contains(jsonOf(b), "compat_tool_disabled") {
		t.Errorf("disabled tool: %d %v, want 409 compat_tool_disabled", code, b)
	}

	// Full table: a new appid is refused, an existing one is still replaced.
	if _, err := h.pool.Exec(ctx, `INSERT INTO compat_tool_mappings (provider, external_id, tool_id)
		SELECT 'steam', g::text, $1::uuid FROM generate_series(1, $2::int) g`, ok, MaxMappings); err != nil {
		t.Fatal(err)
	}
	if code, b := h.do("PUT", path+"5000", h.admin, `{"tool_id":"`+ok+`"}`); code != 409 || !strings.Contains(jsonOf(b), `"code":"conflict"`) {
		t.Errorf("a new appid in a full table: %d %v, want 409 conflict", code, b)
	}
	if code, b := h.do("PUT", path+"7", h.admin, `{"tool_id":"`+ok+`","forced":true}`); code != 200 {
		t.Errorf("replacing in a full table: %d %v, want 200", code, b)
	}
	if a := h.mappingAudit(); len(a) != 1 {
		t.Errorf("audit rows = %q, want only the accepted replace", a)
	}
}

// 150 hosts make the enable row's host list exceed the 4096-byte audit bound; the row must
// still be written, cut to 40 ids and say so.
func TestEnableOnManyHostsStillWritesItsAuditRow(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 150; i++ {
		h.host(fmt.Sprintf("m%d", i), true)
	}
	id, _, _ := h.ge()
	if code, tool := h.enable(id, true); code != http.StatusOK || tool["enabled"] != true {
		t.Fatalf("enable: %d %v", code, tool)
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.enable' AND target_id = $1
		AND jsonb_array_length(details->'host_ids') = 40 AND details->>'host_ids_truncated' = 'true'
		AND (details->>'host_count')::int = (SELECT count(*) FROM hosts)`, id); n != 1 {
		t.Errorf("%d enable audit rows, want 1 (40 ids, truncated, host_count = every host)", n)
	}
}

// A Valve build has no tag or SHA-512: its enable and disable rows carry the Steam name instead.
func TestAValveToolEnableDisableRowCarriesItsSteamName(t *testing.T) {
	h := newHarness(t)
	id := h.id["proton_9"]
	if code, body := h.enable(id, false); code != http.StatusOK || body["enabled"] != false {
		t.Fatalf("disable: %d %v", code, body)
	}
	if code, body := h.enable(id, true); code != http.StatusOK || body["enabled"] != true {
		t.Fatalf("enable: %d %v", code, body)
	}
	for _, action := range []string{"compat_tool.disable", "compat_tool.enable"} {
		if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = $1 AND target_id = $2
			AND details->>'steam_name' = 'proton_9' AND details->'tag' = 'null'::jsonb
			AND details->'sha512' = 'null'::jsonb`, action, id); n != 1 {
			t.Errorf("%s rows carrying the Steam name = %d, want 1", action, n)
		}
	}
}
