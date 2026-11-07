package compat

// addtag_db_test.go — add-by-tag (amendment 56, #1072), its Steam-name binding and pin_conflict,
// and the available list (amendment 57), against a real database (make test-db).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/accreleus/quasar/control-plane/internal/audit"
)

func (h *harness) addTag() string {
	h.t.Helper()
	h.gh.serve = release(addTag, goodAssets(), addSHA+"  "+addAsset+"\n")
	code, out := h.do("POST", "/v1/admin/compat-tools", h.admin, `{"tag":"`+addTag+`"}`)
	if code != http.StatusCreated {
		h.t.Fatalf("add: %d %v", code, out)
	}
	return out["id"].(string)
}

func TestAddByTagPinsTheReleaseChecksumDisabledAndAudits(t *testing.T) {
	h := newHarness(t)
	h.gh.serve = release(addTag, goodAssets(), addSHA+"  "+addAsset+"\n")
	code, tool := h.do("POST", "/v1/admin/compat-tools", h.admin, `{"tag":"`+addTag+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("add: %d %v", code, tool)
	}
	asset, _ := tool["asset"].(map[string]any)
	if tool["source"] != "tag" || tool["family"] != "ge" || tool["enabled"] != false || tool["recommended"] != false ||
		tool["steam_name"] != nil || tool["display_name"] != addTag || tool["hosts"] == nil ||
		asset["sha512"] != addSHA || asset["size_bytes"] != 566859228.0 || asset["name"] != addAsset ||
		asset["url"] != "https://github.com/GloriousEggroll/proton-ge-custom/releases/download/"+addTag+"/"+addAsset {
		t.Errorf("added tool = %v", tool)
	}
	if n := h.count(`SELECT count(*) FROM job_runs WHERE job_id = $1`, JobID); n != 0 {
		t.Error("adding a tag queued an install; nothing installs until it is enabled")
	}
	var actor, details string
	if err := h.pool.QueryRow(context.Background(), `SELECT COALESCE(actor_user_id::text, ''), details::text
		FROM admin_activity WHERE action = 'compat_tool.add' AND target_id = $1`, tool["id"]).Scan(&actor, &details); err != nil || actor == "" {
		t.Fatalf("add audit: actor %q err %v", actor, err)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(details), &d)
	if d["tag"] != addTag || d["sha512"] != addSHA || d["source"] != "tag" || d["asset"] != addAsset {
		t.Errorf("add audit details = %v", d)
	}
	if ids, ok := d["host_ids"].([]any); !ok || len(ids) != 0 {
		t.Errorf("add audit host_ids = %v, want []", d["host_ids"])
	}

	before := len(h.gh.reqs)
	for _, tag := range []string{addTag, recommended[0].Tag} {
		if code, out := h.do("POST", "/v1/admin/compat-tools", h.admin, `{"tag":"`+tag+`"}`); code != http.StatusConflict || out["code"] != "compat_tool_exists" {
			t.Errorf("re-add %s: %d %v", tag, code, out)
		}
	}
	if len(h.gh.reqs) != before {
		t.Error("a tag that exists was resolved on github.com again")
	}
}

func TestAddByTagRefusalsStoreNothing(t *testing.T) {
	h := newHarness(t)
	redirect := func(host string, w http.ResponseWriter, r *http.Request) {
		if host == "github.com" {
			w.Header().Set("Location", "https://evil.example/"+addSum)
			w.WriteHeader(http.StatusFound)
			return
		}
		release(addTag, goodAssets(), "")(host, w, r)
	}
	for _, c := range []struct {
		name, body string
		serve      func(string, http.ResponseWriter, *http.Request)
		status     int
		code, msg  string
	}{
		{"missing checksum", `{"tag":"` + addTag + `"}`, release(addTag, map[string]int64{addAsset: 5}, ""), 422, "compat_tool_unverifiable", "checksum_missing"},
		{"checksum names another file", `{"tag":"` + addTag + `"}`, release(addTag, goodAssets(), addSHA+"  other.tar.gz"), 422, "compat_tool_unverifiable", "checksum_malformed"},
		{"tag not found", `{"tag":"` + addTag + `"}`, release("GE-Proton1-1", goodAssets(), ""), 422, "compat_tool_unverifiable", "release_not_found"},
		{"non-github redirect", `{"tag":"` + addTag + `"}`, redirect, 422, "compat_tool_unverifiable", reasonRedirectRefused},
		{"github down", `{"tag":"` + addTag + `"}`, func(_ string, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }, 503, "upstream_unavailable", ""},
		{"tag outside the grammar", `{"tag":"GE-Proton11-13; rm"}`, nil, 400, "validation_failed", ""},
		{"a url for a tag", `{"tag":"https://evil.example/x.tar.gz"}`, nil, 400, "validation_failed", ""},
		{"unknown key", `{"tag":"` + addTag + `","sha512":"` + addSHA + `"}`, nil, 400, "validation_failed", ""},
		{"no tag", `{}`, nil, 400, "validation_failed", ""},
	} {
		h.gh.serve = c.serve
		if c.serve == nil {
			h.gh.serve = func(string, http.ResponseWriter, *http.Request) { t.Errorf("%s: github.com was read", c.name) }
		}
		code, out := h.do("POST", "/v1/admin/compat-tools", h.admin, c.body)
		if code != c.status || out["code"] != c.code || (c.msg != "" && out["message"] != c.msg) {
			t.Errorf("%s: %d %v, want %d %s %s", c.name, code, out, c.status, c.code, c.msg)
		}
	}
	if n := h.count(`SELECT count(*) FROM compat_tools WHERE source = 'tag'`); n != 0 {
		t.Errorf("%d tag rows stored by refused adds", n)
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.add'`); n != 0 {
		t.Errorf("%d add audit rows for refused adds", n)
	}
}

func TestATagInstallsWithItsPinAndBindsItsSteamName(t *testing.T) {
	h := newHarness(t)
	a := h.host("a", true)
	id := h.addTag()
	if code, out := h.enable(id, true); code != http.StatusOK {
		t.Fatalf("enable: %d %v", code, out)
	}
	runs := h.claim("a")
	if len(runs) != 1 {
		t.Fatalf("claimed %d runs", len(runs))
	}
	p := runs[0]["params"].(map[string]any)
	if p["action"] != "install" || p["tag"] != addTag || p["asset"] != "x86_64" || p["sha512"] != addSHA ||
		p["size_bytes"] != 566859228.0 || p["expected_name"] != nil {
		t.Fatalf("install params = %v, want the tag's pin and no expected name yet", p)
	}
	name := addTag + "-x86_64"
	h.report("a", runs[0]["run_id"].(string), "succeeded", map[string]any{"action": "install", "tool_id": id,
		"name": name, "sha512": addSHA, "bytes": 1, "already_installed": false})
	var steam, display string
	if err := h.pool.QueryRow(context.Background(), `SELECT COALESCE(steam_name, ''), display_name FROM compat_tools WHERE id = $1::uuid`,
		id).Scan(&steam, &display); err != nil || steam != name || display != name {
		t.Fatalf("after the first install: steam_name %q display_name %q (%v), want %q bound", steam, display, err, name)
	}
	if n := h.count(`SELECT count(*) FROM admin_activity WHERE action = 'compat_tool.install' AND target_id = $1 AND details->>'tag' = $2`, a, addTag); n != 1 {
		t.Errorf("%d install audit rows for the tag", n)
	}
	// Every later run carries the bound name, so another host's agent refuses a differing one.
	h.host("b", true)
	h.conv.All(context.Background(), false)
	runs = h.claim("b")
	if len(runs) != 1 || runs[0]["params"].(map[string]any)["expected_name"] != name {
		t.Errorf("host b params = %v, want expected_name %s", runs, name)
	}
}

func TestAFirstReportedNameThatCannotBeBoundMarksANameConflict(t *testing.T) {
	for _, name := range []string{"proton_9", "Proton-GE", recommended[0].SteamName} {
		h := newHarness(t)
		a := h.host("a", true)
		id := h.addTag()
		h.enable(id, true)
		runs := h.claim("a")
		h.report("a", runs[0]["run_id"].(string), "succeeded", map[string]any{"action": "install", "tool_id": id,
			"name": name, "sha512": addSHA, "bytes": 1, "already_installed": false})
		var conflict bool
		var steam *string
		if err := h.pool.QueryRow(context.Background(), `SELECT name_conflict, steam_name FROM compat_tools WHERE id = $1::uuid`,
			id).Scan(&conflict, &steam); err != nil || !conflict || steam != nil {
			t.Errorf("%s: name_conflict %v steam_name %v (%v)", name, conflict, steam, err)
		}
		var actor *string
		var details string
		if err := h.pool.QueryRow(context.Background(), `SELECT actor_user_id::text, details::text FROM admin_activity
			WHERE action = 'compat_tool.name_conflict' AND target_id = $1`, id).Scan(&actor, &details); err != nil || actor != nil ||
			!strings.Contains(details, name) || !strings.Contains(details, a) || !strings.Contains(details, addSHA) {
			t.Errorf("%s: name_conflict audit actor %v details %s (%v)", name, actor, details, err)
		}
		if st := toolState(h.card(a), id); st["state"] != "failed" || st["reason"] != "name_conflict" {
			t.Errorf("%s: host state %v, want failed with name_conflict", name, st)
		}
	}
}

func TestReconcileMarksATagRowWhoseShippedPinDiffers(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	r := recommended[0]
	other := strings.Repeat("cd", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO compat_tools (family, source, tag, display_name, asset_name, asset_size_bytes, asset_sha512)
		VALUES ('ge', 'tag', $1, $1, $2, 5, $3)`, r.Tag, r.Asset, other); err != nil {
		t.Fatal(err)
	}
	s := NewStore(pool)
	s.Auditor = audit.NewStore(pool)
	for i := 0; i < 2; i++ {
		if err := s.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var pin, rec bool
	var source, sha string
	if err := pool.QueryRow(ctx, `SELECT pin_conflict, recommended, source, asset_sha512 FROM compat_tools WHERE tag = $1`, r.Tag).
		Scan(&pin, &rec, &source, &sha); err != nil || !pin || !rec || source != "tag" || sha != other {
		t.Fatalf("pin_conflict %v recommended %v source %s sha %s (%v): want marked, never re-pinned", pin, rec, source, sha, err)
	}
	var n int
	var details string
	if err := pool.QueryRow(ctx, `SELECT count(*), max(details::text) FROM admin_activity
		WHERE action = 'compat_tool.pin_conflict' AND actor_user_id IS NULL`).Scan(&n, &details); err != nil || n != 1 ||
		!strings.Contains(details, other) || !strings.Contains(details, r.SHA512) {
		t.Errorf("%d pin_conflict audit rows (%s), want one system row naming both pins", n, details)
	}

	// The mark clears when the shipped pin matches the row's again.
	if _, err := pool.Exec(ctx, `UPDATE compat_tools SET asset_sha512 = $2 WHERE tag = $1`, r.Tag, r.SHA512); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT pin_conflict FROM compat_tools WHERE tag = $1`, r.Tag).Scan(&pin); err != nil || pin {
		t.Errorf("pin_conflict %v after the pins agree", pin)
	}
}

func TestAvailableListsTheRecommendedThenTheLatestWithLiveCatalogueState(t *testing.T) {
	h := newHarness(t)
	h.host("a", true)
	rec := recommended[0]
	recID, _, _ := h.ge()
	h.exec(`UPDATE compat_tools SET name_conflict = true WHERE id = $1::uuid`, recID)
	tagID := h.addTag() // GE-Proton11-13, now a disabled tag row
	h.exec(`UPDATE compat_tools SET pin_conflict = true WHERE id = $1::uuid`, tagID)
	list := listJSON(ok("GE-Proton11-14", "2026-11-05T09:40:00Z"), ok(addTag, "2026-11-01T00:00:00Z"),
		ok(rec.Tag, "2026-10-30T18:02:11Z"))
	h.gh.serve = func(host string, w http.ResponseWriter, r *http.Request) {
		if host == "api.github.com" && r.URL.Path == "/repos/"+project+"/releases" {
			io.WriteString(w, list)
			return
		}
		http.NotFound(w, r)
	}
	code, out := h.do("GET", "/v1/admin/compat-tools/available", h.admin, "")
	if code != http.StatusOK {
		t.Fatalf("available: %d %v", code, out)
	}
	gl := out["github_latest"].(map[string]any)
	if gl["available"] != true || gl["reason"] != nil || gl["fetched_at"] == nil {
		t.Errorf("github_latest = %v", gl)
	}
	items := out["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	want := []struct {
		tag, source, display string
		published, toolID    any
		pin, name            bool
		hosts                bool
	}{
		{rec.Tag, "recommended", rec.SteamName, "2026-10-30T18:02:11Z", recID, false, true, true},
		{"GE-Proton11-14", "github_latest", "GE-Proton11-14", "2026-11-05T09:40:00Z", nil, false, false, false},
		{addTag, "github_latest", addTag, "2026-11-01T00:00:00Z", tagID, true, false, true},
	}
	for i, w := range want {
		m := items[i].(map[string]any)
		if m["tag"] != w.tag || m["source"] != w.source || m["display_name"] != w.display || m["published_at"] != w.published ||
			m["tool_id"] != w.toolID || m["pin_conflict"] != w.pin || m["name_conflict"] != w.name ||
			(m["hosts"] != nil) != w.hosts || m["enabled"] != false {
			t.Errorf("item %d = %v, want %+v", i, m, w)
		}
	}

	// Catalogue state is read live, never from the cache.
	h.exec(`UPDATE compat_tools SET name_conflict = false WHERE id = $1::uuid`, recID)
	_, out = h.do("GET", "/v1/admin/compat-tools/available", h.admin, "")
	if m := out["items"].([]any)[0].(map[string]any); m["name_conflict"] != false {
		t.Errorf("a cleared mark still reads %v", m["name_conflict"])
	}
	if n := h.gh.hit("api.github.com"); n != 2 { // one add-by-tag metadata read, one list fetch
		t.Errorf("%d api.github.com requests, want the list fetched once", n)
	}
}

func TestAvailableIs200WithOnlyTheRecommendedWhenGitHubFails(t *testing.T) {
	h := newHarness(t)
	h.gh.serve = func(_ string, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	code, out := h.do("GET", "/v1/admin/compat-tools/available", h.admin, "")
	if code != http.StatusOK {
		t.Fatalf("available: %d %v", code, out)
	}
	gl := out["github_latest"].(map[string]any)
	if gl["available"] != false || gl["reason"] != "upstream_error" || gl["fetched_at"] != nil {
		t.Errorf("github_latest = %v", gl)
	}
	items := out["items"].([]any)
	if len(items) != len(recommended) || items[0].(map[string]any)["source"] != "recommended" ||
		items[0].(map[string]any)["published_at"] != nil {
		t.Errorf("items = %v, want only the recommended list with no dates", items)
	}
}
