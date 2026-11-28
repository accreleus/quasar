package crud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestParseHomeLayout(t *testing.T) {
	ok := map[string]string{
		"builtin order":       `{"sections":[{"id":"highlights","kind":"highlights","title":"Highlights","enabled":true},{"id":"fav","kind":"favourites","title":"Mine","enabled":false,"params":{}}]}`,
		"empty list":          `{"sections":[]}`,
		"read body sent back": `{"source":"admin","sections":[{"id":"a","kind":"jump_back_in","title":"Again","enabled":true,"params":{}}]}`,
		"repeated kind":       `{"sections":[{"id":"a","kind":"favourites","title":"A","enabled":true},{"id":"b","kind":"favourites","title":"B","enabled":true}]}`,
		"64 code point title": `{"sections":[{"id":"a","kind":"favourites","title":"` + strings.Repeat("é", 64) + `","enabled":true}]}`,
	}
	for name, body := range ok {
		if _, err := parseHomeLayout([]byte(body)); err != nil {
			t.Errorf("%s: want ok, got %v", name, err)
		}
	}
	sixteen := make([]string, 17)
	for i := range sixteen {
		sixteen[i] = `{"id":"s` + string(rune('a'+i)) + `","kind":"favourites","title":"T","enabled":true}`
	}
	bad := map[string]string{
		"not an object":       `[]`,
		"no sections":         `{}`,
		"sections null":       `{"sections":null}`,
		"too many":            `{"sections":[` + strings.Join(sixteen, ",") + `]}`,
		"duplicate id":        `{"sections":[{"id":"a","kind":"favourites","title":"A","enabled":true},{"id":"a","kind":"highlights","title":"B","enabled":true}]}`,
		"unknown kind":        `{"sections":[{"id":"a","kind":"continue_watching","title":"A","enabled":true}]}`,
		"bad id":              `{"sections":[{"id":"Has Space","kind":"favourites","title":"A","enabled":true}]}`,
		"missing enabled":     `{"sections":[{"id":"a","kind":"favourites","title":"A"}]}`,
		"empty title":         `{"sections":[{"id":"a","kind":"favourites","title":"","enabled":true}]}`,
		"whitespace title":    `{"sections":[{"id":"a","kind":"favourites","title":"   ","enabled":true}]}`,
		"65 code point title": `{"sections":[{"id":"a","kind":"favourites","title":"` + strings.Repeat("é", 65) + `","enabled":true}]}`,
		"extra section key":   `{"sections":[{"id":"a","kind":"favourites","title":"A","enabled":true,"colour":"red"}]}`,
		"params not object":   `{"sections":[{"id":"a","kind":"favourites","title":"A","enabled":true,"params":null}]}`,
		"undefined param":     `{"sections":[{"id":"a","kind":"favourites","title":"A","enabled":true,"params":{"limit":5}}]}`,
	}
	for name, body := range bad {
		if _, err := parseHomeLayout([]byte(body)); err == nil {
			t.Errorf("%s: want a validation error, got none", name)
		}
	}
}

func homeReq(t *testing.T, method, url, body, bearer string) (int, homeLayout) {
	t.Helper()
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out homeLayout
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestHomeLayoutLayers: built-in, then the admin default for a fresh user, then
// the user's override, and clearing each falls back one layer.
func TestHomeLayoutLayers(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newAuditedTestServer(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE instance_settings SET home_layout = NULL`); err != nil {
		t.Fatalf("reset admin default: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `UPDATE instance_settings SET home_layout = NULL`) })

	login := func(email, name string, admin bool) string {
		if _, err := authSvc.Register(ctx, email, name, "quasar-fixture-pw-01"); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		if admin {
			if _, err := pool.Exec(ctx, `UPDATE users SET role = 'admin' WHERE email = $1`, email); err != nil {
				t.Fatalf("promote: %v", err)
			}
		}
		tok, err := authSvc.Login(ctx, email, "quasar-fixture-pw-01", "")
		if err != nil {
			t.Fatalf("login %s: %v", name, err)
		}
		return tok.Plaintext
	}
	adminTok := login("admin@test.local", "admin", true)
	userTok := login("user@test.local", "user", false)
	me, adm := srv.URL+"/v1/me/home", srv.URL+"/v1/admin/home"

	code, got := homeReq(t, http.MethodGet, me, "", userTok)
	if code != 200 || got.Source != "builtin" || len(got.Sections) != 3 || got.Sections[1].Kind != "jump_back_in" || got.Sections[1].Params == nil {
		t.Fatalf("builtin: %d %+v", code, got)
	}

	if code, _ := homeReq(t, http.MethodPut, adm, `{"sections":[]}`, userTok); code != http.StatusForbidden {
		t.Fatalf("user PUT admin default: want 403, got %d", code)
	}

	adminBody := `{"sections":[{"id":"fav","kind":"favourites","title":"Your picks","enabled":true},{"id":"highlights","kind":"highlights","title":"Highlights","enabled":false}]}`
	if code, got := homeReq(t, http.MethodPut, adm, adminBody, adminTok); code != 200 || got.Source != "admin" || len(got.Sections) != 2 {
		t.Fatalf("admin PUT: %d %+v", code, got)
	}
	code, got = homeReq(t, http.MethodGet, me, "", userTok)
	if code != 200 || got.Source != "admin" || got.Sections[0].Title != "Your picks" || got.Sections[1].Enabled {
		t.Fatalf("fresh user sees admin default: %d %+v", code, got)
	}

	if code, _ := homeReq(t, http.MethodPut, me, `{"sections":[{"id":"x","kind":"nope","title":"X","enabled":true}]}`, userTok); code != http.StatusBadRequest {
		t.Fatalf("unknown kind on write: want 400, got %d", code)
	}
	code, got = homeReq(t, http.MethodPut, me, `{"sections":[{"id":"jb","kind":"jump_back_in","title":"Again","enabled":true}]}`, userTok)
	if code != 200 || got.Source != "user" || len(got.Sections) != 1 || got.Sections[0].Title != "Again" {
		t.Fatalf("user PUT: %d %+v", code, got)
	}
	if code, got := homeReq(t, http.MethodGet, me, "", userTok); got.Source != "user" {
		t.Fatalf("override wins: %d %+v", code, got)
	}
	// The override lives outside the ui-preferences document.
	var overlay string
	if err := pool.QueryRow(ctx, `SELECT session_overlay::text FROM user_ui_preferences u JOIN users ON users.id = u.user_id WHERE email = 'user@test.local'`).Scan(&overlay); err != nil || overlay != "{}" {
		t.Fatalf("session_overlay: %q %v", overlay, err)
	}
	if code, got := homeReq(t, http.MethodGet, adm, "", adminTok); got.Source != "admin" {
		t.Fatalf("admin GET is the default, not the admin's own view: %d %+v", code, got)
	}

	if code, got := homeReq(t, http.MethodDelete, me, "", userTok); code != 200 || got.Source != "admin" {
		t.Fatalf("clearing override falls back to admin: %d %+v", code, got)
	}
	if code, got := homeReq(t, http.MethodDelete, me, "", userTok); code != 200 || got.Source != "admin" {
		t.Fatalf("clear is idempotent: %d %+v", code, got)
	}
	if code, got := homeReq(t, http.MethodDelete, adm, "", adminTok); code != 200 || got.Source != "builtin" || len(got.Sections) != 3 {
		t.Fatalf("clearing admin default: %d %+v", code, got)
	}
	if _, got := homeReq(t, http.MethodGet, me, "", userTok); got.Source != "builtin" {
		t.Fatalf("user back on builtin: %+v", got)
	}
	var actions []string
	rows, err := pool.Query(ctx, `SELECT action || ':' || target_type FROM admin_activity WHERE action LIKE 'home_layout.%' ORDER BY id`)
	if err != nil {
		t.Fatalf("read activity: %v", err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	if strings.Join(actions, ",") != "home_layout.set:instance,home_layout.cleared:instance" {
		t.Fatalf("activity: %v (the user's override is never recorded)", actions)
	}
}
