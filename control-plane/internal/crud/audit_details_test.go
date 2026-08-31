package crud

// #385 item 8 — the admin audit log rendered `{}` for most actions because
// almost every recordActivity call site passed nil details. These tests assert
// what each audited crud action now carries, and that the payload stays inside
// admin_activity's `CHECK (octet_length(details::text) <= 4096)` (migration
// 0028) — the constraint that makes struct dumps unsafe and changed-FIELD-NAME
// diffs the idiom.
//
// Require Postgres (TEST_DATABASE_URL); one shared DB, truncate in setup, -p 1.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/accreleus/quasar/control-plane/internal/audit"
	"github.com/accreleus/quasar/control-plane/internal/auth"
)

// newAuditedTestServer is newTestServer with a real audit store wired in, so the
// activity rows a request writes can be read straight back out of the DB.
func newAuditedTestServer(t *testing.T, pool *pgxpool.Pool) (*httptest.Server, *auth.Service) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DELETE FROM admin_activity`); err != nil {
		t.Fatalf("truncate admin_activity: %v", err)
	}
	authSvc, err := auth.NewService(pool, auth.DefaultParams(), time.Hour)
	if err != nil {
		t.Fatalf("auth service: %v", err)
	}
	mux := http.NewServeMux()
	authHandler := auth.NewHandler(authSvc)
	authHandler.Register(mux)
	crudHandler := NewHandler(pool, audit.NewStore(pool))
	crudHandler.Register(mux, authHandler.RequireAuth, authHandler.RequireAdmin)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, authSvc
}

// auditDetails reads the single most recent activity row for an action, together
// with the byte length Postgres sees for the CHECK.
func auditDetails(t *testing.T, pool *pgxpool.Pool, action string) (map[string]any, int) {
	t.Helper()
	var raw []byte
	var size int
	err := pool.QueryRow(context.Background(), `
		SELECT details, octet_length(details::text)
		FROM admin_activity WHERE action = $1 ORDER BY id DESC LIMIT 1
	`, action).Scan(&raw, &size)
	if err != nil {
		t.Fatalf("no admin_activity row for %q: %v", action, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("details for %q is not a JSON object: %v (%s)", action, err, raw)
	}
	if size > 4096 {
		t.Fatalf("details for %q is %d bytes, over migration 0028's 4096-byte CHECK", action, size)
	}
	return out, size
}

func keyList(t *testing.T, details map[string]any, action string) []string {
	t.Helper()
	rawKeys, ok := details["keys"].([]any)
	if !ok {
		t.Fatalf("%s details has no `keys` array: %#v", action, details)
	}
	out := make([]string, 0, len(rawKeys))
	for _, k := range rawKeys {
		s, ok := k.(string)
		if !ok {
			t.Fatalf("%s details `keys` holds a non-string: %#v", action, k)
		}
		out = append(out, s)
	}
	return out
}

// TestAuditDetails_RuntimePresetLifecycle: create/update/delete each record a
// non-empty payload, the update records CHANGED FIELD NAMES only, and no VALUE
// from the request body reaches the log.
func TestAuditDetails_RuntimePresetLifecycle(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newAuditedTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "audit@preset.test", "auditpreset")

	resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "Audited Preset",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	created, _ := auditDetails(t, pool, "runtime_preset.create")
	if created["name"] != "Audited Preset" {
		t.Errorf("runtime_preset.create details = %#v, want name=\"Audited Preset\"", created)
	}

	// PATCH two fields, one of which is env — the field most likely to hold a
	// credential. The audit row must name it and never quote it.
	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{
		"image": "quasar-agent-dev:latest",
		"env":   map[string]any{"SECRET_TOKEN": "hunter2"},
	}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch preset: want 200, got %d (%v)", resp.StatusCode, body)
	}
	updated, size := auditDetails(t, pool, "runtime_preset.update")
	keys := keyList(t, updated, "runtime_preset.update")
	if len(keys) != 2 || keys[0] != "env" || keys[1] != "image" {
		t.Errorf("runtime_preset.update keys = %v, want [env image]", keys)
	}
	rawUpdated, _ := json.Marshal(updated)
	for _, leaked := range []string{"hunter2", "quasar-agent-dev:latest"} {
		if strings.Contains(string(rawUpdated), leaked) {
			t.Errorf("runtime_preset.update details leaked a VALUE (%q): %s", leaked, rawUpdated)
		}
	}
	t.Logf("runtime_preset.update details: %d bytes", size)

	resp = deleteReq(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, bearer)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete preset: want 204, got %d", resp.StatusCode)
	}
	deleted, _ := auditDetails(t, pool, "runtime_preset.delete")
	if deleted["name"] != "Audited Preset" {
		t.Errorf("runtime_preset.delete details = %#v, want name=\"Audited Preset\"", deleted)
	}
}

// TestAuditDetails_UnchangedPatchRecordsNoKeys: an empty PATCH must record an
// EMPTY key list rather than every field name. A key list that claims fields
// changed which the request never carried is worse than no detail at all.
func TestAuditDetails_UnchangedPatchRecordsNoKeys(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newAuditedTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "audit@nochange.test", "auditnochange")

	resp, body := post(t, srv.URL+"/v1/admin/runtime-presets", map[string]any{
		"name": "Untouched",
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create preset: want 201, got %d (%v)", resp.StatusCode, body)
	}
	presetID := body["runtime_preset"].(map[string]any)["id"].(string)

	resp, body = patch(t, srv.URL+"/v1/admin/runtime-presets/"+presetID, map[string]any{}, bearer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty patch: want 200, got %d (%v)", resp.StatusCode, body)
	}
	updated, _ := auditDetails(t, pool, "runtime_preset.update")
	if keys := keyList(t, updated, "runtime_preset.update"); len(keys) != 0 {
		t.Errorf("empty PATCH recorded keys %v, want an empty list", keys)
	}
}

// TestAuditDetails_AppAndHostDelete: the two crud deletes name what was removed.
// After the delete the uuid in target_id points at nothing, so the name is the
// only part of the row that still means anything.
func TestAuditDetails_AppAndHostDelete(t *testing.T) {
	pool := testDB(t)
	srv, authSvc := newAuditedTestServer(t, pool)
	ctx := context.Background()
	bearer := adminBearer(t, ctx, pool, authSvc, "audit@delete.test", "auditdelete")

	resp, body := post(t, srv.URL+"/v1/apps", map[string]any{
		"name":                 "Doomed App",
		"description":          "about to be deleted",
		"default_width":        1280,
		"default_height":       720,
		"default_fps":          30,
		"default_bitrate_kbps": 5000,
		"default_vram_mb":      2048,
		"default_encode_slots": 1,
		"runtime_spec":         map[string]any{"image": "quasar-agent-dev:latest"},
	}, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create app: want 201, got %d (%v)", resp.StatusCode, body)
	}
	appID := body["app"].(map[string]any)["id"].(string)

	if r := deleteReq(t, srv.URL+"/v1/apps/"+appID, bearer); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete app: want 204, got %d", r.StatusCode)
	}
	appDetails, _ := auditDetails(t, pool, "app.delete")
	if appDetails["name"] != "Doomed App" {
		t.Errorf("app.delete details = %#v, want name=\"Doomed App\"", appDetails)
	}

	// Hosts are agent-registered, not created over the API — seed one directly.
	var hostID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO hosts (node_name, status) VALUES ('doomed-node','offline') RETURNING id::text`,
	).Scan(&hostID); err != nil {
		t.Fatalf("seed host: %v", err)
	}
	if r := deleteReq(t, srv.URL+"/v1/hosts/"+hostID, bearer); r.StatusCode != http.StatusNoContent {
		t.Fatalf("delete host: want 204, got %d", r.StatusCode)
	}
	hostDetails, _ := auditDetails(t, pool, "host.delete")
	if hostDetails["node_name"] != "doomed-node" {
		t.Errorf("host.delete details = %#v, want node_name=\"doomed-node\"", hostDetails)
	}
}
